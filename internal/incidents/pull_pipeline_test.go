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

package incidents

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analyzer/collector"
)

// pullManager registers the real collector reconcilers without starting informers or an API server.
type pullManager struct {
	ctrl.Manager
	cl          client.Client
	reconcilers []reconcile.Reconciler
}

func (m *pullManager) GetScheme() *runtime.Scheme  { return m.cl.Scheme() }
func (m *pullManager) GetClient() client.Client    { return m.cl }
func (m *pullManager) GetAPIReader() client.Reader { return m.cl }
func (*pullManager) GetCache() cache.Cache         { return nil }
func (*pullManager) GetControllerOptions() config.Controller {
	return config.Controller{SkipNameValidation: new(true)}
}
func (*pullManager) GetLogger() logr.Logger { return logr.Discard() }
func (m *pullManager) Add(r manager.Runnable) error {
	reconciler, ok := r.(reconcile.Reconciler)
	if !ok {
		return fmt.Errorf("unexpected collector runnable %T", r)
	}
	m.reconcilers = append(m.reconcilers, reconciler)
	return nil
}

func TestAlternatingPullWaitsOpenOneIncident(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: foldConsequenceUID},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "web:v1"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", Image: "web:v1",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}},
		}}},
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Incident{}).WithObjects(pod).Build()
	c := loaded(t, cl)
	now := triggerNow
	c.cfg.Now = func() time.Time { return now }
	col, err := collector.New(collector.Config{ClusterID: "test", Now: c.cfg.Now, Handled: c.Handled})
	if err != nil {
		t.Fatal(err)
	}
	m := &pullManager{cl: cl}
	if err := col.SetupWithManager(m); err != nil {
		t.Fatal(err)
	}
	for i := range 24 {
		pod.Status.ContainerStatuses[0].State.Waiting.Reason = []string{reasonErrImage, reasonImagePull}[i%2]
		if err := cl.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			for _, r := range m.reconcilers {
				if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pod)}); err != nil {
					t.Fatal(err)
				}
			}
			for len(col.Observations()) > 0 {
				c.consume(t.Context(), <-col.Observations())
			}
			now = now.Add(collector.SettleAfter)
		}
	}
	c.flushDue(t.Context(), true)
	var list api.IncidentList
	if err := cl.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || !list.Items[0].Status.Cause || list.Items[0].Status.Count != 1 {
		t.Fatalf("alternating pull waits opened %d Incidents, want one cause with one occurrence", len(list.Items))
	}
}
