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

package collector

import (
	"context"
	"slices"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/kyverno"
)

// policyCRD is a minimal CRD of Kyverno's namespaced Policy.
func policyCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "policies." + kyverno.PolicyGroup},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: kyverno.PolicyGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "policies", Singular: "policy", Kind: kyverno.KindPolicy, ListKind: kyverno.KindPolicy + "List"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: kyverno.PolicyVersion, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: new(true),
				}},
			}},
		},
	}
}

// TestEnvtestLazyWatchStartsOnceServed runs the real lazy watch against an API server, as the binary sets it
// up: with Kyverno active and its Policy CRD not installed, the Policy kind is reported as not served and no
// informer is asked for; once the CRD is installed, the next check starts the watch (an informer for Policy
// in the manager's cache) and the kind is no longer reported. The manager runs as an administrator, so the
// grant path is covered by the unit tests.
func TestEnvtestLazyWatchStartsOnceServed(t *testing.T) {
	startEnv(t)
	cfg := env.testEnv.Config
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var rec *recordingCache
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// The suite's manager runs the same controllers in this process.
		Controller: config.Controller{SkipNameValidation: new(true)},
		Cache:      analyzer.CacheOptions(operatorNS, []string{watchedNS}),
		NewCache: func(c *rest.Config, opts cache.Options) (cache.Cache, error) {
			inner, err := cache.New(c, opts)
			if err != nil {
				return nil, err
			}
			rec = &recordingCache{Cache: inner, scheme: scheme}
			return rec, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := modules.New(nil, []modules.Integration{kyverno.Module()})
	if err != nil {
		t.Fatal(err)
	}
	sel := reg.Select(modules.Enabled{Integrations: []string{kyverno.Name}},
		reg.Detect(nil, []string{kyverno.PolicyGroup + "/" + kyverno.PolicyVersion}))
	col, err := New(Config{
		ClusterID: testCluster, Namespaces: []string{watchedNS}, SelectionPoll: 200 * time.Millisecond,
		Modules: func() modules.Selection { return sel },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := col.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = mgr.Start(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })

	policy := gvkPolicy.GroupVersion().String() + "/" + gvkPolicy.Kind
	state := func() WatchState {
		for _, w := range col.SourcesNotWatched() {
			if w.Kind == gvkPolicy {
				return w.State
			}
		}
		return WatchWatched
	}
	waitFor(t, func() bool { return state() == WatchNotServed })
	if slices.Contains(rec.informers(), policy) {
		t.Fatal("an informer was asked for a kind that is not served")
	}

	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{policyCRD()}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return state() == WatchWatched && slices.Contains(rec.informers(), policy) })
}

// waitFor polls cond for up to 30 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
