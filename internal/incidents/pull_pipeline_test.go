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
	eventsv1 "k8s.io/api/events/v1"
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

const (
	pullPodKind       = "Pod"
	pipelineSpecImage = "web:v1"
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

func (m *pullManager) reconcile(t *testing.T, obj client.Object) {
	t.Helper()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	for _, r := range m.reconcilers {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAlternatingPullWaitsAndDetailedFailures(t *testing.T) {
	for _, tc := range []struct{ name, image string }{
		{"matching", pipelineSpecImage},
		{"different", "web:v2"},
		{"unparsable", "bad::image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkPullPipeline(t, tc.image)
		})
	}
}

func checkPullPipeline(t *testing.T, reportedImage string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: foldContainer, Namespace: "shop", UID: foldConsequenceUID},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: foldContainer, Image: pipelineSpecImage}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: foldContainer, Image: pipelineSpecImage,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonErrImage}},
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
	col, err := collector.New(collector.Config{ClusterID: "test", Now: c.cfg.Now, Handled: c.Handled, CauseSeen: c.CauseSeen})
	if err != nil {
		t.Fatal(err)
	}
	m := &pullManager{cl: cl}
	if err := col.SetupWithManager(m); err != nil {
		t.Fatal(err)
	}
	consume := func(obj client.Object) {
		m.reconcile(t, obj)
		for len(col.Observations()) > 0 {
			c.consume(t.Context(), <-col.Observations())
		}
	}

	// A recurring detailed failure opens its own Incident before the collector observes the pod's wait.
	failed := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-event", Namespace: pod.Namespace, UID: "event-uid", CreationTimestamp: metav1.NewTime(now.Add(-RecurAfter))},
		Regarding:  corev1.ObjectReference{APIVersion: "v1", Kind: pullPodKind, Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID, FieldPath: "spec.containers{" + foldContainer + "}"},
		Reason:     reasonFailed, Type: corev1.EventTypeWarning, ReportingController: kubeletController,
		Note:      fmt.Sprintf("Failed to pull image %q: not found", reportedImage),
		EventTime: metav1.NewMicroTime(now.Add(-RecurAfter)),
		Series:    &eventsv1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(now)},
	}
	if err := cl.Create(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	consume(failed)
	c.flushDue(t.Context(), true)

	var list api.IncidentList
	if err := cl.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("detailed failure opened %d Incidents before the wait was observed, want 1", len(list.Items))
	}
	detail := &list.Items[0]
	detailName := detail.Name
	if detail.Spec.PullCauseFingerprint != "" || detail.Status.Consequence || foldCandidate(detail) {
		t.Fatalf("detailed failure is eligible to fold: %+v", detail)
	}

	for i := range 24 {
		pod.Status.ContainerStatuses[0].State.Waiting.Reason = []string{reasonErrImage, reasonImagePull}[i%2]
		if err := cl.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			consume(pod)
			now = now.Add(collector.SettleAfter)
		}
	}

	// A short Failed note counts on the pull cause while the detailed failure stays separate.
	short := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "short-pull-event", Namespace: pod.Namespace, UID: "short-event-uid", CreationTimestamp: metav1.NewTime(now)},
		Regarding:  failed.Regarding,
		Reason:     reasonFailed, Type: corev1.EventTypeWarning, ReportingController: kubeletController,
		Note:      "Error: ImagePullBackOff",
		EventTime: metav1.NewMicroTime(now),
	}
	if err := cl.Create(t.Context(), short); err != nil {
		t.Fatal(err)
	}
	consume(short)
	c.flushDue(t.Context(), true)
	if err := c.foldRecent(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cl.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("got %d Incidents, want one detailed failure and one pull cause", len(list.Items))
	}
	for _, inc := range list.Items {
		if inc.Name == detailName {
			if inc.Status.Count != 2 || inc.Status.Folded || inc.Status.FoldedInto != nil || inc.Status.Phase != api.IncidentOpen {
				t.Fatalf("detailed failure did not stay open: %+v", inc.Status)
			}
			continue
		}
		if !inc.Status.Cause || inc.Status.Count != 1 || len(inc.Status.Absorbed) != 1 ||
			inc.Status.Absorbed[0].Reason != api.AbsorbedFailed || inc.Status.Absorbed[0].Count != 1 {
			t.Fatalf("alternating waits and the short Failed note did not share one cause: %+v", inc.Status)
		}
	}
}
