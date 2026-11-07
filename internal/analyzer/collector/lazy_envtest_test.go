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

	"github.com/prometheus/client_golang/prometheus/testutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// lazyEnv is a manager of its own, with the analyzer's cache options and a cache that records its informers,
// and a collector with Kyverno active, watching watchedNS.
type lazyEnv struct {
	mgr ctrl.Manager
	col *Collector
	rec *recordingCache
}

// newLazyEnv sets up the manager and the collector; lazyConfig, when set, is the config of the lazy watches'
// caches. Call start once the test is ready.
func newLazyEnv(t *testing.T, lazyConfig *rest.Config) *lazyEnv {
	t.Helper()
	startEnv(t)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	e := &lazyEnv{}
	mgr, err := ctrl.NewManager(env.testEnv.Config, ctrl.Options{
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
			e.rec = &recordingCache{Cache: inner, scheme: scheme}
			return e.rec, nil
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
	col.lazyConfig = lazyConfig
	if err := col.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	e.mgr, e.col = mgr, col
	return e
}

// start runs the manager until the test ends, and fails the test if it stops before.
func (e *lazyEnv) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- e.mgr.Start(ctx) }()
	t.Cleanup(func() {
		select {
		case err := <-stopped:
			cancel()
			t.Errorf("the manager stopped before the test ended: %v", err)
			return
		default:
		}
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("the manager failed: %v", err)
		}
	})
}

// state is the Policy kind's watch state as SourcesNotWatched reports it: Watched once it is complete without
// the kind, "" while not complete.
func (e *lazyEnv) state() WatchState {
	kinds, complete := e.col.SourcesNotWatched()
	for _, w := range kinds {
		if w.Kind == gvkPolicy {
			return w.State
		}
	}
	if complete {
		return WatchWatched
	}
	return ""
}

var policyKind = gvkPolicy.GroupVersion().String() + "/" + gvkPolicy.Kind

// installPolicyCRD installs the Policy CRD, once for the suite's API server.
func installPolicyCRD(t *testing.T) {
	t.Helper()
	if _, err := envtest.InstallCRDs(env.testEnv.Config, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{policyCRD()}}); err != nil {
		t.Fatal(err)
	}
}

// TestEnvtestLazyWatchStartsOnceServed runs the real lazy watch against an API server, as the binary sets it
// up: with Kyverno active and its Policy CRD not installed, the Policy kind is reported as not served; once the
// CRD is installed, the next check starts the watch, on a cache of its own (the manager's cache never gets a
// Policy informer), and its reconciler reads the Policy objects from that cache. The manager runs as an
// administrator, so the grant path is covered by the unit tests and the next test.
func TestEnvtestLazyWatchStartsOnceServed(t *testing.T) {
	e := newLazyEnv(t, nil)
	e.start(t)
	waitFor(t, func() bool { return e.state() == WatchNotServed })
	installPolicyCRD(t)
	waitFor(t, func() bool { return e.state() == WatchWatched })
	policy := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{},
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "Failed", "message": "not ready",
			"lastTransitionTime": "2026-11-07T10:00:00Z",
		}}},
	}}
	policy.SetGroupVersionKind(gvkPolicy)
	policy.SetNamespace(watchedNS)
	policy.SetName("not-ready")
	if err := env.admin.Create(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	// The symptom waits SettleAfter before it is emitted; its state is tracked at the first reconcile.
	waitFor(t, func() bool { return e.col.conditions.len() > 0 })
	if slices.Contains(e.rec.informers(), policyKind) {
		t.Error("the manager's cache got an informer for a lazily watched kind")
	}
}

// TestEnvtestLazyWatchThatCannotSync (#409 review): the lazy watch's cache runs as an identity with no grant on
// Policies while the check (the API reader) may list them, as when the grant is removed between the check and
// the watch's first list. The informer cannot sync. The refusal stops the watch and the kind is reported as
// failed; the manager's cache never holds that informer, so it still syncs and the manager keeps running.
func TestEnvtestLazyWatchThatCannotSync(t *testing.T) {
	installPolicyCRD(t)
	denied := rest.CopyConfig(env.testEnv.Config)
	denied.Impersonate = rest.ImpersonationConfig{UserName: "lazy-watch-without-grant"}
	e := newLazyEnv(t, denied)
	e.start(t)
	waitFor(t, func() bool { return e.state() == WatchFailed })
	if slices.Contains(e.rec.informers(), policyKind) {
		t.Error("the manager's cache got an informer for a lazily watched kind")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if !e.mgr.GetCache().WaitForCacheSync(ctx) {
		t.Error("the manager's cache did not sync while a lazy watch could not")
	}
	if g := testutil.ToFloat64(symptomSourceWatched.WithLabelValues(gvkPolicy.Group, gvkPolicy.Kind)); g != 0 {
		t.Errorf("gauge %v, want 0", g)
	}
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
