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
	"maps"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testNS      = "shop"
	testCluster = "lab-1"
	web         = "web"
	webRS       = "web-7d4b9c8f6"
	webPod      = "web-7d4b9c8f6-q8wzt"
	reasonBack  = "BackOff"
	reasonError = "Error"
	// containerApp is the container name of the test pods.
	containerApp = "app"
	// labelApp is the label key the test workloads select on.
	labelApp = "app.kubernetes.io/name"
	intruder = "intruder"
)

var (
	t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	deployUID = types.UID("4f1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d10")
	rsUID     = types.UID("5a1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d11")
	podUID    = types.UID("0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d")
	stsUID    = types.UID("6b1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d12")
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// clock is a settable test clock.
type clock struct{ now atomic.Pointer[time.Time] }

func newClock(at time.Time) *clock {
	c := &clock{}
	c.set(at)
	return c
}
func (c *clock) set(at time.Time)    { c.now.Store(&at) }
func (c *clock) Now() time.Time      { return *c.now.Load() }
func (c *clock) add(d time.Duration) { c.set(c.Now().Add(d)) }

// harness wires a collector to a fake client, as SetupWithManager does to a manager.
type harness struct {
	t      *testing.T
	c      *Collector
	cl     client.WithWatch
	clock  *clock
	events *eventReconciler
	pods   *podReconciler
	gets   atomic.Int64 // owner reads through the API reader
}

func newHarness(t *testing.T, cfg Config, objs ...client.Object) *harness {
	t.Helper()
	h := &harness{t: t, clock: newClock(t0)}
	h.cl = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	h.init(cfg, h.cl)
	return h
}

func (h *harness) init(cfg Config, api client.Reader) {
	h.t.Helper()
	if cfg.ClusterID == "" {
		cfg.ClusterID = testCluster
	}
	if cfg.Now == nil {
		cfg.Now = h.clock.Now
	}
	if cfg.SendTimeout == 0 {
		cfg.SendTimeout = 50 * time.Millisecond
	}
	c, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	counting := interceptor.NewClient(h.cl, interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			h.gets.Add(1)
			return api.Get(ctx, key, obj)
		},
	})
	c.owners = newOwnerResolver(h.cl, counting, c.cfg)
	h.c = c
	h.events = &eventReconciler{c: c, reader: h.cl}
	h.pods = &podReconciler{c: c, reader: h.cl}
}

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: name}}
}

func (h *harness) reconcileEvent(name string) ctrl.Result {
	h.t.Helper()
	res, err := h.events.Reconcile(h.t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: name}})
	if err != nil {
		h.t.Fatalf("event reconcile: %v", err)
	}
	return res
}

func (h *harness) reconcilePod(name string) ctrl.Result {
	h.t.Helper()
	res, err := h.pods.Reconcile(h.t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: name}})
	if err != nil {
		h.t.Fatalf("pod reconcile: %v", err)
	}
	return res
}

// drain returns every observation waiting in the channel.
func (h *harness) drain() []Observation {
	var out []Observation
	for {
		select {
		case o := <-h.c.Observations():
			out = append(out, o)
		default:
			return out
		}
	}
}

// one returns the single waiting observation.
func (h *harness) one() Observation {
	h.t.Helper()
	got := h.drain()
	if len(got) != 1 {
		h.t.Fatalf("got %d observations, want 1: %+v", len(got), got)
	}
	return got[0]
}

func (h *harness) none() {
	h.t.Helper()
	if got := h.drain(); len(got) != 0 {
		h.t.Fatalf("got %d observations, want none: %+v", len(got), got)
	}
}

func (h *harness) create(obj client.Object) {
	h.t.Helper()
	if err := h.cl.Create(h.t.Context(), obj); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) update(obj client.Object) {
	h.t.Helper()
	// Status first: an Update writes the stored object back, which would reset the status to set.
	if _, ok := obj.(*corev1.Pod); ok {
		if err := h.cl.Status().Update(h.t.Context(), obj); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.cl.Update(h.t.Context(), obj); err != nil {
		h.t.Fatal(err)
	}
}

// warning returns a Warning event about the web pod, created at the test start time.
func warning(name string, series int32) *eventsv1.Event {
	ev := &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: types.UID("ev-" + name),
			CreationTimestamp: metav1.NewTime(t0),
		},
		EventTime:           metav1.NewMicroTime(t0),
		Reason:              reasonBack,
		Action:              "Restarting",
		Type:                corev1.EventTypeWarning,
		ReportingController: reportingKubelet,
		ReportingInstance:   "node-1",
		Note:                "Back-off 10s restarting failed container app in pod " + webPod,
		Regarding: corev1.ObjectReference{
			APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: webPod, UID: podUID,
		},
	}
	if series > 0 {
		ev.Series = &eventsv1.EventSeries{Count: series, LastObservedTime: metav1.NewMicroTime(t0)}
	}
	return ev
}

func controllerRef(apiVersion, kind, name string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: new(true)}}
}

// workload returns a Deployment, its ReplicaSet and a pod of it, with the labels and selectors their
// controllers would set.
func workload() (*appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod) {
	appLabels := map[string]string{labelApp: web}
	rsLabels := map[string]string{labelApp: web, "pod-template-hash": "7d4b9c8f6"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: web, Namespace: testNS, UID: deployUID},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: appLabels}},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: webRS, Namespace: testNS, UID: rsUID, Labels: rsLabels,
			OwnerReferences: controllerRef(apiApps, kindDeployment, web, deployUID),
		},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: rsLabels}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: webPod, Namespace: testNS, UID: podUID, Labels: maps.Clone(rsLabels),
		OwnerReferences: controllerRef(apiApps, kindReplicaSet, webRS, rsUID),
	}}
	return d, rs, pod
}

// withTermination sets one terminated last state on container app of pod. Its message holds a secret that
// must never reach an observation.
func withTermination(pod *corev1.Pod, containerID, reason string, exitCode int32, finished time.Time) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         containerApp,
		RestartCount: 1,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: containerID, Reason: reason, ExitCode: exitCode, FinishedAt: metav1.NewTime(finished),
			Message: "password=" + "hunter2" + "-synthetic",
		}},
	}}
	return pod
}
