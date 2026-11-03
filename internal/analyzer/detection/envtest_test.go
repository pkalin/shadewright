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

package detection

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/aks"
)

const (
	kedaName = "keda"
	kedaGV   = "keda.sh/v1alpha1"
)

// keda is a detection-only integration for these tests.
type keda struct{}

func (keda) Name() string { return kedaName }
func (keda) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{kedaGV}}
}
func (keda) Hooks() modules.Hooks { return modules.Hooks{} }

var env struct {
	once    sync.Once
	err     error
	testEnv *envtest.Environment
	admin   client.Client
	reg     *modules.Registry
	det     *Detector // run by the manager
	cancel  context.CancelFunc
}

func TestMain(m *testing.M) {
	code := m.Run()
	if env.cancel != nil {
		env.cancel()
	}
	if env.testEnv != nil {
		_ = env.testEnv.Stop()
	}
	os.Exit(code)
}

func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "bin", "k8s")
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

func servesKeda(context.Context) ([]string, error) { return []string{"v1", "apps/v1", kedaGV}, nil }

func startEnv(t *testing.T) {
	t.Helper()
	env.once.Do(func() { env.err = bootEnv() })
	if env.err != nil {
		t.Fatalf("starting envtest: %v", env.err)
	}
}

func bootEnv() error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	env.testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: firstEnvtestBinaryDir(),
	}
	cfg, err := env.testEnv.Start()
	if err != nil {
		return err
	}
	if env.admin, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		return err
	}
	if env.reg, err = modules.New([]modules.Platform{aks.Platform{}}, []modules.Integration{keda{}}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	for i, name := range []string{"aks-nodepool1-12345678-vmss000000", "aks-nodepool1-12345678-vmss000001"} {
		n := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{aks.ClusterLabel: "mc_rg"}},
			Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/s/resourceGroups/mc_rg/providers/" +
				"Microsoft.Compute/virtualMachineScaleSets/aks-nodepool1-12345678-vmss/virtualMachines/" + string(rune('0'+i))},
		}
		if err := env.admin.Create(ctx, n); err != nil {
			return err
		}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return err
	}
	// A long interval: the manager's detector runs only at start and on spec changes in these tests.
	if env.det, err = New(Config{Registry: env.reg, GroupVersions: servesKeda, Interval: time.Hour}); err != nil {
		return err
	}
	if err := env.det.SetupWithManager(mgr); err != nil {
		return err
	}
	go func() { _ = mgr.Start(ctx) }()
	return nil
}

func settings(t *testing.T) *remediationv1alpha1.RemediationSettings {
	t.Helper()
	var s remediationv1alpha1.RemediationSettings
	if err := env.admin.Get(t.Context(), client.ObjectKey{Name: remediationv1alpha1.SettingsName}, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

// eventually polls cond for up to 20 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func setModules(t *testing.T, m *remediationv1alpha1.Modules) {
	t.Helper()
	s := settings(t)
	s.Spec.Modules = m
	if err := env.admin.Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
}

// TestEnvtestDetectionIsWrittenToStatus runs the detector in a manager against a real API server: it lists
// the nodes, works out the active modules from spec.modules and writes them to status, with the condition
// that names an enabled module the binary does not have.
func TestEnvtestDetectionIsWrittenToStatus(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	s := &remediationv1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
		Spec: remediationv1alpha1.RemediationSettingsSpec{Modules: &remediationv1alpha1.Modules{
			Platforms:    []remediationv1alpha1.ModuleName{aks.Name},
			Integrations: []remediationv1alpha1.ModuleName{kedaName, "kedda"},
		}},
	}
	if err := env.admin.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.admin.Delete(context.Background(), s) })

	wantPlatform := remediationv1alpha1.PlatformStatus{Detected: aks.Name, Active: aks.Name}
	wantInts := []remediationv1alpha1.IntegrationStatus{
		{Name: kedaName, Detected: true, Version: kedaGV, Active: true},
		{Name: "kedda"},
	}
	eventually(t, "the status reports aks and keda", func() bool {
		st := settings(t).Status
		return st.Platform != nil && *st.Platform == wantPlatform && reflect.DeepEqual(st.Integrations, wantInts)
	})
	got := settings(t)
	c := meta.FindStatusCondition(got.Status.Conditions, ConditionUnknownModules)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonUnknownModulesEnabled ||
		c.ObservedGeneration != got.Generation || got.Status.ObservedGeneration != got.Generation {
		t.Errorf("condition = %+v, observedGeneration %d, generation %d", c, got.Status.ObservedGeneration, got.Generation)
	}
	if sel := env.det.Selection(); sel.ActivePlatform() != aks.Name {
		t.Errorf("active platform = %q, want aks", sel.ActivePlatform())
	}

	// Disabling aks takes effect on the spec change: detected stays aks, active becomes generic.
	setModules(t, nil)
	eventually(t, "the status reports aks detected and generic active", func() bool {
		st := settings(t).Status
		return st.Platform != nil && st.Platform.Active == remediationv1alpha1.GenericPlatform &&
			st.Platform.Detected == aks.Name
	})
	c = meta.FindStatusCondition(settings(t).Status.Conditions, ConditionUnknownModules)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("condition after the unknown name was removed = %+v", c)
	}
	if env.det.Selection().ActivePlatform() != modules.GenericPlatform {
		t.Errorf("active platform = %q after aks was disabled", env.det.Selection().ActivePlatform())
	}
}

// TestEnvtestForgedStatusEnablesNothing writes a status that says aks and keda are active while spec enables
// neither, and checks that a run of the detector does not take it as input: generic stays the only active
// module, and the status is rewritten from spec and detection.
func TestEnvtestForgedStatusEnablesNothing(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	s := &remediationv1alpha1.RemediationSettings{ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName}}
	if err := env.admin.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.admin.Delete(context.Background(), s) })
	// Let the manager's detector write first, so the forged status is the last word before the run below.
	eventually(t, "the manager's detector reports generic active", func() bool {
		st := settings(t).Status
		return st.Platform != nil && st.Platform.Active == remediationv1alpha1.GenericPlatform
	})
	forged := settings(t)
	forged.Status.Platform = &remediationv1alpha1.PlatformStatus{Detected: aks.Name, Active: aks.Name}
	forged.Status.Integrations = []remediationv1alpha1.IntegrationStatus{
		{Name: kedaName, Detected: true, Version: kedaGV, Active: true}}
	if err := env.admin.Status().Update(ctx, forged); err != nil {
		t.Fatal(err)
	}

	d, err := New(Config{Registry: env.reg, GroupVersions: servesKeda})
	if err != nil {
		t.Fatal(err)
	}
	d.reader, d.writer = env.admin, env.admin
	res, err := d.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: remediationv1alpha1.SettingsName}})
	if err != nil || res.RequeueAfter != DefaultInterval {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	sel := d.Selection()
	if sel.ActivePlatform() != modules.GenericPlatform || len(sel.Active()) != 1 {
		t.Errorf("active = %q %+v, want generic only", sel.ActivePlatform(), sel.Active())
	}
	st := settings(t).Status
	if st.Platform == nil || st.Platform.Active != remediationv1alpha1.GenericPlatform || st.Platform.Detected != aks.Name {
		t.Errorf("platform status = %+v, want aks detected and generic active", st.Platform)
	}
	want := []remediationv1alpha1.IntegrationStatus{{Name: kedaName, Detected: true, Version: kedaGV}}
	if !reflect.DeepEqual(st.Integrations, want) {
		t.Errorf("integrations = %+v, want %+v", st.Integrations, want)
	}
}

// TestEnvtestNoSettingsIsGenericOnly checks that without a RemediationSettings object the detector keeps
// generic only, writes nothing and schedules no periodic run.
func TestEnvtestNoSettingsIsGenericOnly(t *testing.T) {
	startEnv(t)
	d, err := New(Config{Registry: env.reg, GroupVersions: servesKeda})
	if err != nil {
		t.Fatal(err)
	}
	d.reader, d.writer = env.admin, env.admin
	res, err := d.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: remediationv1alpha1.SettingsName}})
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v; want no periodic run without settings", res, err)
	}
	if got := d.Selection(); got.ActivePlatform() != modules.GenericPlatform || got.DetectedPlatform() != aks.Name {
		t.Errorf("selection = %s detected, %s active", got.DetectedPlatform(), got.ActivePlatform())
	}
}
