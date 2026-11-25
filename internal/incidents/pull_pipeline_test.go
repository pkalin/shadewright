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
	"crypto/sha256"
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
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

const pullPodKind = "Pod"

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

func TestAlternatingPullWaitsOpenOneIncident(t *testing.T) {
	runAlternatingPull(t, false, false)
}

func TestUpgradeDuringPullKeepsIncident(t *testing.T) {
	t.Run("waiting pod first", func(t *testing.T) { runAlternatingPull(t, true, false) })
	t.Run("pull event first", func(t *testing.T) { runAlternatingPull(t, true, true) })
}

func runAlternatingPull(t *testing.T, upgrade, eventFirst bool) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: foldContainer, Namespace: "shop", UID: foldConsequenceUID},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: foldContainer, Image: "web:v1"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: foldContainer, Image: "web:v1",
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
	var oldName string
	if upgrade {
		oldName = seedPreviousPull(t, cl, pod)
	}
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
	var failed collector.Observation
	for i := range 24 {
		pod.Status.ContainerStatuses[0].State.Waiting.Reason = []string{reasonErrImage, reasonImagePull}[i%2]
		if err := cl.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
		if eventFirst && i == 0 {
			failed = collectUpgradeEvent(t, m, col, pod, now)
			c.consume(t.Context(), failed)
		}
		for range 2 {
			m.reconcile(t, pod)
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
	if len(list.Items) != 1 || (upgrade && list.Items[0].Name != oldName) || !list.Items[0].Status.Cause || list.Items[0].Status.Count != 1 {
		t.Fatalf("alternating pull waits opened %d Incidents, want one cause with one occurrence", len(list.Items))
	}
	if !eventFirst {
		return
	}
	inc := &list.Items[0]
	if inc.Status.ContinuedPullFingerprint != failed.CauseFingerprint || len(inc.Status.Occurrences) != 1 ||
		len(inc.Status.Absorbed) != 1 || inc.Status.Absorbed[0].Reason != api.AbsorbedFailed || inc.Status.Absorbed[0].Count != 1 {
		t.Fatalf("continued pull status: %+v", inc.Status)
	}

	// A later container restart has a new waiting occurrence. The persisted image identity
	// lets a new analyzer link its pull event to the original Incident.
	restarted := loaded(t, cl)
	for i := range failed.PreviousPullWaits {
		failed.PreviousPullWaits[i].Occurrence = waitingID(17 + i)
	}
	if !restarted.causeSeen(failed) {
		t.Fatal("restart lost the continued pull")
	}
	changed := failed
	changed.CauseFingerprint = fp(99)
	if restarted.causeSeen(changed) {
		t.Fatal("continued pull accepted a different image")
	}

	inc.Status.Phase, inc.Status.ResolvedTime = api.IncidentResolved, &metav1.Time{Time: now}
	if err := cl.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	if loaded(t, cl).causeSeen(failed) {
		t.Fatal("resolved cause still matched the continued pull")
	}
}

func seedPreviousPull(t *testing.T, cl client.Client, pod *corev1.Pod) string {
	t.Helper()
	// Before pull identities included the image, each reason had its own fingerprint
	// and each occurrence hashed the pod UID, container, reason and restart count.
	input := fingerprint.Observation{ClusterID: "test", Namespace: pod.Namespace, ResourceUID: string(pod.UID),
		Reason: reasonImagePull, Type: corev1.EventTypeWarning, ReportingController: kubeletController,
		Note: "Container " + foldContainer + " is waiting: ImagePullBackOff", RegardingKind: pullPodKind, RegardingName: pod.Name,
		Source: fingerprint.SourcePodStatus}
	sum := sha256.Sum256([]byte(string(pod.UID) + "\x00" + foldContainer + "\x00waiting-ImagePullBackOff-restarts-0"))
	o := pullCause(reasonImagePull, triggerNow)
	o.Fingerprint, o.Symptom = fingerprint.Fingerprint(input), fingerprint.Symptom(input)
	o.Source = pod.Namespace
	o.Subject = api.ObjectRef{APIVersion: "v1", Kind: pullPodKind, Namespace: pod.Namespace, Name: pod.Name, UID: api.UID(pod.UID)}
	o.Occurrence = fmt.Sprintf("waiting:%x", sum[:16])
	inc := storedPullObservation(t, foldCauseUID, o, triggerNow)
	if err := cl.Create(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	return inc.Name
}

func collectUpgradeEvent(t *testing.T, m *pullManager, col *collector.Collector, pod *corev1.Pod, now time.Time) collector.Observation {
	t.Helper()
	// The event informer may run before the pod reconciler after the upgrade.
	ev := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-event", Namespace: pod.Namespace, UID: "event-uid", CreationTimestamp: metav1.NewTime(now)},
		Regarding:  corev1.ObjectReference{APIVersion: "v1", Kind: pullPodKind, Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID, FieldPath: "spec.containers{" + foldContainer + "}"},
		Reason:     reasonFailed, Type: corev1.EventTypeWarning, ReportingController: kubeletController, Note: `Failed to pull image "web:v1": not found`,
		EventTime: metav1.NewMicroTime(now),
	}
	if err := m.cl.Create(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	m.reconcile(t, ev)
	select {
	case failed := <-col.Observations():
		return failed
	default:
		t.Fatal("verified pull event waited for a cause that was already open")
	}
	return collector.Observation{}
}
