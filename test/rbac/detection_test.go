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

package rbac

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	analyzerpkg "github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/detection"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/aks"
)

// TestDetectionRunsWithTheAnalyzerRBAC runs the detector as the analyzer's ServiceAccount, with the analyzer's
// cache options and the binary's discovery function: the settings watch, the node list, the settings read,
// discovery and the status update must all fit the analyzer's grants. A missing grant shows up as a status
// that is never written.
func TestDetectionRunsWithTheAnalyzerRBAC(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "aks-rbac-12345678-vmss000000",
			Labels: map[string]string{aks.ClusterLabel: "mc_rg"},
		},
		Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/s/resourceGroups/mc_rg/vm/0"},
	}
	settings := &remediationv1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
		Spec: remediationv1alpha1.RemediationSettingsSpec{Modules: &remediationv1alpha1.Modules{
			Platforms: []remediationv1alpha1.ModuleName{aks.Name}}},
	}
	for _, obj := range []client.Object{node, settings} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = admin.Delete(context.Background(), settings)
		_ = admin.Delete(context.Background(), node)
	})

	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: user(analyzer), Groups: saGroups}
	mgr, err := ctrl.NewManager(c, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  analyzerpkg.CacheOptions(operatorNS, []string{otherNS}),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := modules.New([]modules.Platform{aks.Platform{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	det, err := detection.New(detection.Config{Registry: reg, GroupVersions: detection.DiscoveryGroupVersions(c)})
	if err != nil {
		t.Fatal(err)
	}
	if err := det.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	want := remediationv1alpha1.PlatformStatus{Detected: aks.Name, Active: aks.Name}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var got remediationv1alpha1.RemediationSettings
		if err := admin.Get(ctx, client.ObjectKeyFromObject(settings), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Platform != nil && *got.Status.Platform == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v after 30 s, want %+v written by the analyzer", got.Status, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
