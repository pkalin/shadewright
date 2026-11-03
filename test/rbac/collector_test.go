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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	analyzerpkg "github.com/pkalin/shadewright/internal/analyzer"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// TestCollectorRunsWithTheAnalyzerRBAC runs the collector as the analyzer's ServiceAccount, with the cache
// restricted to the watched namespace: its watches and its owner reads must all fit the analyzer's grants.
// A missing grant shows up as a cache that never syncs or an owner that is never resolved.
func TestCollectorRunsWithTheAnalyzerRBAC(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	labels := map[string]string{"app": "rbac-web"}
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app:1"}}},
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-web", Namespace: otherNS},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: tmpl},
	}
	if err := admin.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-web-7d4b9c8f6", Namespace: otherNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: new(true),
			}}},
		Spec: appsv1.ReplicaSetSpec{Selector: d.Spec.Selector, Template: tmpl},
	}
	if err := admin.Create(ctx, rs); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-web-7d4b9c8f6-q8wzt", Namespace: otherNS, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true),
			}}},
		Spec: tmpl.Spec,
	}
	if err := admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	// Other tests count the pods in the watched namespace.
	t.Cleanup(func() {
		for _, obj := range []client.Object{pod, rs, d} {
			_ = admin.Delete(context.Background(), obj, client.GracePeriodSeconds(0))
		}
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
	col, err := collector.New(collector.Config{ClusterID: "rbac-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := col.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	now := metav1.NewMicroTime(time.Now())
	ev := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-web-backoff", Namespace: otherNS},
		EventTime:  now, Reason: "BackOff", Action: "Restarting", Type: corev1.EventTypeWarning,
		ReportingController: "kubelet", ReportingInstance: "node-1", Note: "Back-off restarting failed container",
		Regarding: corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: otherNS, Name: pod.Name, UID: pod.UID},
	}
	if err := admin.Create(ctx, ev); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(30 * time.Second)
	for {
		select {
		case o := <-col.Observations():
			if o.Reason != "BackOff" {
				continue
			}
			if o.Subject.Kind != "Deployment" || o.Subject.Name != d.Name {
				t.Fatalf("subject = %+v, want Deployment %s: an owner read was refused", o.Subject, d.Name)
			}
			return
		case <-timeout:
			t.Fatal("no observation: the collector's watches or reads do not fit the analyzer's RBAC")
		}
	}
}
